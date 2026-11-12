// num_comm_params=0
// num_mand_comm_params=0
// num_opt_comm_params=0

// git_link=https://github.com//blob///home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_replication.go#L3
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
	chan child_BackupReplicationDropsDestinationVerification30 = [1] of {int}->
	run BackupReplicationDropsDestinationVerification3(child_BackupReplicationDropsDestinationVerification30)->
	child_BackupReplicationDropsDestinationVerification30?0->
stop_process:skip
}

proctype BackupReplicationDropsDestinationVerification3(chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	chan child_AnonymousBackupReplicationDropsDestinationVerification1344 = [1] of {int}->
	chan child_AnonymousBackupReplicationDropsDestinationVerification1243 = [1] of {int}->
	chan child_AnonymousBackupReplicationDropsDestinationVerification1182 = [1] of {int}->
	chan child_AnonymousBackupReplicationDropsDestinationVerification1071 = [1] of {int}->
	chan child_AnonymousBackupReplicationDropsDestinationVerification950 = [1] of {int}->
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
	run AnonymousBackupReplicationDropsDestinationVerification95(transferred_ch,receipt_ch,content_ch,confirmed_ch,intent_ch,child_AnonymousBackupReplicationDropsDestinationVerification950)->
	run receiver(child_AnonymousBackupReplicationDropsDestinationVerification950)->
	run AnonymousBackupReplicationDropsDestinationVerification107(content_ch,confirmed_ch,intent_ch,transferred_ch,receipt_ch,child_AnonymousBackupReplicationDropsDestinationVerification1071)->
	run receiver(child_AnonymousBackupReplicationDropsDestinationVerification1071)->
	run AnonymousBackupReplicationDropsDestinationVerification118(confirmed_ch,intent_ch,transferred_ch,receipt_ch,content_ch,child_AnonymousBackupReplicationDropsDestinationVerification1182)->
	run receiver(child_AnonymousBackupReplicationDropsDestinationVerification1182)->
	run AnonymousBackupReplicationDropsDestinationVerification124(intent_ch,transferred_ch,receipt_ch,content_ch,confirmed_ch,child_AnonymousBackupReplicationDropsDestinationVerification1243)->
	run receiver(child_AnonymousBackupReplicationDropsDestinationVerification1243)->
	run AnonymousBackupReplicationDropsDestinationVerification134(intent_ch,transferred_ch,receipt_ch,content_ch,confirmed_ch,child_AnonymousBackupReplicationDropsDestinationVerification1344)->
	run receiver(child_AnonymousBackupReplicationDropsDestinationVerification1344)->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_replication.go:14:2 */
	:: confirmed_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_replication.go:14:2 */->
	:: confirmed_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_replication.go:14:2 */ -> 
		confirmed_ch.rcving!false
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousBackupReplicationDropsDestinationVerification95(Chandef transferred_ch;Chandef receipt_ch;Chandef content_ch;Chandef confirmed_ch;Chandef intent_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_replication.go:9:12 */
	:: intent_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_replication.go:9:12 */
	:: intent_ch.sync!false -> 
		intent_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_replication.go:9:12 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousBackupReplicationDropsDestinationVerification107(Chandef content_ch;Chandef confirmed_ch;Chandef intent_ch;Chandef transferred_ch;Chandef receipt_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_replication.go:10:12 */
	:: intent_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_replication.go:10:12 */->
	:: intent_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_replication.go:10:12 */ -> 
		intent_ch.rcving!false
	fi->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_replication.go:10:22 */
	:: transferred_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_replication.go:10:22 */
	:: transferred_ch.sync!false -> 
		transferred_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_replication.go:10:22 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousBackupReplicationDropsDestinationVerification118(Chandef confirmed_ch;Chandef intent_ch;Chandef transferred_ch;Chandef receipt_ch;Chandef content_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_replication.go:11:12 */
	:: transferred_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_replication.go:11:12 */->
	:: transferred_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_replication.go:11:12 */ -> 
		transferred_ch.rcving!false
	fi->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_replication.go:11:27 */
	:: receipt_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_replication.go:11:27 */
	:: receipt_ch.sync!false -> 
		receipt_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_replication.go:11:27 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousBackupReplicationDropsDestinationVerification124(Chandef intent_ch;Chandef transferred_ch;Chandef receipt_ch;Chandef content_ch;Chandef confirmed_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Rcv	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_replication.go:12:12 */
	:: receipt_ch.deq?state,num_msgs /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_replication.go:12:12 */->
	:: receipt_ch.sync?state /* 	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_replication.go:12:12 */ -> 
		receipt_ch.rcving!false
	fi->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_replication.go:12:23 */
	:: content_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_replication.go:12:23 */
	:: content_ch.sync!false -> 
		content_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_replication.go:12:23 */
	fi->
	stop_process: skip->
	child!0
}
proctype AnonymousBackupReplicationDropsDestinationVerification134(Chandef intent_ch;Chandef transferred_ch;Chandef receipt_ch;Chandef content_ch;Chandef confirmed_ch;chan child) {
	bool closed-> 
	bool ok-> 
	int i->
	bool state = true->
	int num_msgs->
	

	if /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_replication.go:13:12 */
	:: confirmed_ch.enq?ok -> 
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_replication.go:13:12 */
	:: confirmed_ch.sync!false -> 
		confirmed_ch.sending?ok->
		assert(ok) /* Send	/home/lbethay/workplace/fae/.tools/gomela-pinned/fae-run.OHULKK/negative/backup_replication.go:13:12 */
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

