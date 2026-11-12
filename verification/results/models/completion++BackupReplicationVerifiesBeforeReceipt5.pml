// num_comm_params=0
// num_mand_comm_params=0
// num_opt_comm_params=0

// git_link=https://github.com//blob///home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/backup_replication.go#L5
typedef Chandef {
	chan sync = [0] of {bool};
	chan enq = [0] of {bool};
	chan deq = [0] of {bool,bool};
	chan sending = [0] of {bool};
	chan rcving = [0] of {bool};
	chan closing = [0] of {bool};
	int size = 0;
	int num_msgs = 0;
	bool closed = false;
}



init { 
	chan child_BackupReplicationVerifiesBeforeReceipt50 = [1] of {int}->
	run BackupReplicationVerifiesBeforeReceipt5(child_BackupReplicationVerifiesBeforeReceipt50)->
	child_BackupReplicationVerifiesBeforeReceipt50?0->
stop_process:skip
}

proctype BackupReplicationVerifiesBeforeReceipt5(chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	chan child_AnonymousBackupReplicationVerifiesBeforeReceipt1564 = [1] of {int}->
	chan child_AnonymousBackupReplicationVerifiesBeforeReceipt1463 = [1] of {int}->
	chan child_AnonymousBackupReplicationVerifiesBeforeReceipt1372 = [1] of {int}->
	chan child_AnonymousBackupReplicationVerifiesBeforeReceipt1281 = [1] of {int}->
	chan child_AnonymousBackupReplicationVerifiesBeforeReceipt1160 = [1] of {int}->
	Chandef confirmed_ch->
	Chandef content_ch->
	Chandef receipt_ch->
	Chandef transferred_ch->
	Chandef intent_ch->
	run sync_monitor(intent_ch)->
	run sync_monitor(transferred_ch)->
	run sync_monitor(receipt_ch)->
	run sync_monitor(content_ch)->
	run sync_monitor(confirmed_ch)->
	run AnonymousBackupReplicationVerifiesBeforeReceipt116(intent_ch,transferred_ch,receipt_ch,content_ch,confirmed_ch,child_AnonymousBackupReplicationVerifiesBeforeReceipt1160)->
	run receiver(child_AnonymousBackupReplicationVerifiesBeforeReceipt1160)->
	run AnonymousBackupReplicationVerifiesBeforeReceipt128(receipt_ch,content_ch,confirmed_ch,intent_ch,transferred_ch,child_AnonymousBackupReplicationVerifiesBeforeReceipt1281)->
	run receiver(child_AnonymousBackupReplicationVerifiesBeforeReceipt1281)->
	run AnonymousBackupReplicationVerifiesBeforeReceipt137(transferred_ch,receipt_ch,content_ch,confirmed_ch,intent_ch,child_AnonymousBackupReplicationVerifiesBeforeReceipt1372)->
	run receiver(child_AnonymousBackupReplicationVerifiesBeforeReceipt1372)->
	run AnonymousBackupReplicationVerifiesBeforeReceipt146(intent_ch,transferred_ch,receipt_ch,content_ch,confirmed_ch,child_AnonymousBackupReplicationVerifiesBeforeReceipt1463)->
	run receiver(child_AnonymousBackupReplicationVerifiesBeforeReceipt1463)->
	run AnonymousBackupReplicationVerifiesBeforeReceipt156(intent_ch,transferred_ch,receipt_ch,content_ch,confirmed_ch,child_AnonymousBackupReplicationVerifiesBeforeReceipt1564)->
	run receiver(child_AnonymousBackupReplicationVerifiesBeforeReceipt1564)->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/backup_replication.go:16:2 */
	:: confirmed_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/backup_replication.go:16:2 */->
	:: confirmed_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/backup_replication.go:16:2 */ -> 
		confirmed_ch.rcving!false
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousBackupReplicationVerifiesBeforeReceipt116(Chandef intent_ch;Chandef transferred_ch;Chandef receipt_ch;Chandef content_ch;Chandef confirmed_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/backup_replication.go:11:14 */
	:: intent_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/backup_replication.go:11:14 */
	:: intent_ch.sync!false -> 
		intent_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/backup_replication.go:11:14 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousBackupReplicationVerifiesBeforeReceipt128(Chandef receipt_ch;Chandef content_ch;Chandef confirmed_ch;Chandef intent_ch;Chandef transferred_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/backup_replication.go:12:14 */
	:: intent_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/backup_replication.go:12:14 */->
	:: intent_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/backup_replication.go:12:14 */ -> 
		intent_ch.rcving!false
	fi->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/backup_replication.go:12:24 */
	:: transferred_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/backup_replication.go:12:24 */
	:: transferred_ch.sync!false -> 
		transferred_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/backup_replication.go:12:24 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousBackupReplicationVerifiesBeforeReceipt137(Chandef transferred_ch;Chandef receipt_ch;Chandef content_ch;Chandef confirmed_ch;Chandef intent_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/backup_replication.go:13:14 */
	:: transferred_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/backup_replication.go:13:14 */->
	:: transferred_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/backup_replication.go:13:14 */ -> 
		transferred_ch.rcving!false
	fi->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/backup_replication.go:13:29 */
	:: receipt_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/backup_replication.go:13:29 */
	:: receipt_ch.sync!false -> 
		receipt_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/backup_replication.go:13:29 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousBackupReplicationVerifiesBeforeReceipt146(Chandef intent_ch;Chandef transferred_ch;Chandef receipt_ch;Chandef content_ch;Chandef confirmed_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/backup_replication.go:14:14 */
	:: receipt_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/backup_replication.go:14:14 */->
	:: receipt_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/backup_replication.go:14:14 */ -> 
		receipt_ch.rcving!false
	fi->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/backup_replication.go:14:25 */
	:: content_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/backup_replication.go:14:25 */
	:: content_ch.sync!false -> 
		content_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/backup_replication.go:14:25 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousBackupReplicationVerifiesBeforeReceipt156(Chandef intent_ch;Chandef transferred_ch;Chandef receipt_ch;Chandef content_ch;Chandef confirmed_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/backup_replication.go:15:14 */
	:: content_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/backup_replication.go:15:14 */->
	:: content_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/backup_replication.go:15:14 */ -> 
		content_ch.rcving!false
	fi->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/backup_replication.go:15:25 */
	:: confirmed_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/backup_replication.go:15:25 */
	:: confirmed_ch.sync!false -> 
		confirmed_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/source/backup_replication.go:15:25 */
	fi->
	stop_process: skip->
	child!0
}

 /* ================================================================================== */
 /* ================================================================================== */
 /* ================================================================================== */ 
proctype async_monitor(Chandef ch) {
do
:: true ->
if
:: ch.closed -> 
end: if
  :: ch.num_msgs > 0 -> // cannot send on closed channel
    end4: if
    :: ch.enq!false-> // cannot send on closed channel
    :: ch.closing!true -> // cannot close twice a channel
    :: ch.rcving?false->
    :: ch.sending!false -> // sending state of channel (closed)
    :: ch.deq!true,ch.num_msgs -> 
  		 ch.num_msgs = ch.num_msgs - 1
    fi->
  :: else ->    end5: if
    :: ch.enq!false-> // cannot send on closed channel
    :: ch.closing!true -> // cannot close twice a channel
    :: ch.rcving?false->
    :: ch.sending!false -> // sending state of channel (closed)
    :: ch.sync!true-> 
    fi->
  fi->
:: else ->
	if
	:: ch.num_msgs == ch.size ->
		end1: if
		  :: ch.deq!false,ch.num_msgs ->
		    ch.num_msgs = ch.num_msgs - 1
		  :: ch.closing!false -> // closing the channel
		    ch.closed = true
		   :: ch.rcving?false ->
 		    ch.sending!true->
		fi->
	:: ch.num_msgs == 0 -> 
end2:		if
		:: ch.enq!true -> // a message has been received
			ch.num_msgs = ch.num_msgs + 1
		:: ch.closing!false -> // closing the channel
			ch.closed = true
		:: ch.rcving?false ->
 		    ch.sending!true->
		fi->
		:: else -> 
		end3: if
		  :: ch.enq!true->
		     ch.num_msgs = ch.num_msgs + 1
		  :: ch.deq!false,ch.num_msgs->
		     ch.num_msgs = ch.num_msgs - 1
		  :: ch.closing!false -> // closing the channel
		     ch.closed = true
		  :: ch.rcving?false ->
 		    ch.sending!true->
		fi->
	fi->
fi->
od->
}

proctype sync_monitor(Chandef ch) {
do
:: true->
if
:: ch.closed ->
end: if
  :: ch.enq!false-> // cannot send on closed channel
  :: ch.closing!true -> // cannot close twice a channel
  :: ch.sending!false -> // sending state of channel (closed)
  :: ch.rcving?false->
  :: ch.sync!true-> // can always receive on a closed chan
  fi->
:: else -> 
end1: if
    :: ch.rcving?false ->
       ch.sending!true->
    :: ch.closing!false ->
      ch.closed = true
    fi->
fi->
od->
stop_process: skip
}

proctype receiver(chan c) {
c?0
}

